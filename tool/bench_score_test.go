package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func benchFileHashes(t *testing.T, root string) map[string]string {
	t.Helper()
	hashes := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hashes[rel] = benchHash(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hashes
}
func benchAssertOriginals(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := benchFileHashes(t, root)
	for path, hash := range before {
		if after[path] != hash {
			t.Errorf("original changed: %s", path)
		}
	}
}
func TestBenchScoreRescorePreservesOriginals(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	r := benchRecordFixture(t, h, "old", 12, false)
	k := benchKeyFixture(t, h, r)
	// Exercise the CLI on an isolated copy, never a live store.
	source := benchDir()
	copied := t.TempDir()
	t.Setenv("ORCH_BENCH_DIR", copied)
	if err := benchCopy(source, copied); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, copied, map[string]string{"fixture/keys/mapping.json": `{"schema_version":1,"mappings":[]}`})
	before := benchFileHashes(t, copied)
	sourceBefore := benchFileHashes(t, source)
	out := h.must("bench", "key", "rescore", "fixture", r.ID, "--key", k.ID)
	benchAssertOriginals(t, copied, before)
	var artifact benchScoreArtifact
	if err := json.Unmarshal([]byte(out), &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.ScorerSchema != benchScorerSchema || artifact.Profile != benchScorerProfile || !benchHex64.MatchString(artifact.PopulationID) {
		t.Fatalf("missing versions or population: %+v", artifact)
	}
	if artifact.MappingID != before[filepath.Join("fixture", "keys", "mapping.json")] {
		t.Fatalf("mapping hash %s", artifact.MappingID)
	}
	if artifact.Metrics["class/duplicate/recall"].Denominator != 1 || artifact.Metrics["answers/group/coverage"].Denominator != 1 || len(artifact.Warnings["legacy"]) == 0 {
		t.Fatalf("missing denominator/exclusions/warnings: %+v", artifact)
	}
	inputPaths := map[string]bool{}
	for _, input := range artifact.Inputs {
		inputPaths[input.Path] = true
		if input.State == "recorded" && input.SHA256 != before[filepath.FromSlash(input.Path)] {
			t.Errorf("wrong input hash: %+v", input)
		}
	}
	for _, rel := range []string{"fixture/runs/" + r.ID + "/run.json", "fixture/runs/" + r.ID + "/snapshot/gate.json", "objects/blob/" + r.Blobs["prompts"][0], "fixture/keys/" + k.ID + ".json", "fixture/keys/" + k.ID + ".original.json"} {
		if !inputPaths[rel] {
			t.Errorf("missing input %s", rel)
		}
	}
	first := benchFileHashes(t, copied)
	h.now = h.now.Add(24 * time.Hour)
	if out2 := h.must("bench", "key", "rescore", "fixture", r.ID, "--key", k.ID); out2 != out {
		t.Fatal("rescore output is not deterministic")
	}
	if !reflect.DeepEqual(first, benchFileHashes(t, copied)) {
		t.Fatal("rescore created duplicate or changed artifacts")
	}
	files, err := filepath.Glob(filepath.Join(copied, "fixture", "runs", r.ID, "scores", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("artifacts %v %v", files, err)
	}
	b := mustRead(t, files[0])
	if filepath.Base(files[0]) != benchHash(b)+".json" {
		t.Fatal("artifact is not content addressed")
	}
	benchAssertOriginals(t, source, sourceBefore)
}

func benchCopiedFixture(t *testing.T) (*harness, *benchRun, *benchKey, string) {
	t.Helper()
	h := benchHarness(t)
	h.gateRepo(1)
	r := benchRecordFixture(t, h, "old", 12, false)
	k := benchKeyFixture(t, h, r)
	source := benchDir()
	copied := t.TempDir()
	t.Setenv("ORCH_BENCH_DIR", copied)
	if err := benchCopy(source, copied); err != nil {
		t.Fatal(err)
	}
	r, err := benchRunLoad("fixture", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	return h, r, k, source
}

func TestBenchScoreValidationFailureStopsWrite(t *testing.T) {
	for _, corrupt := range []string{"snapshot", "key", "blob", "store", "unbound-missing-blob"} {
		t.Run(corrupt, func(t *testing.T) {
			h, r, k, source := benchCopiedFixture(t)
			path := filepath.Join(r.dir, "snapshot", "gate.json")
			if corrupt == "key" {
				path = filepath.Join(benchDir(), "fixture", "keys", k.ID+".original.json")
			}
			if corrupt == "blob" {
				var err error
				path, err = benchObjectPath("blob", r.Blobs["prompts"][0])
				if err != nil {
					t.Fatal(err)
				}
			}
			if corrupt == "unbound-missing-blob" {
				var raw map[string]json.RawMessage
				if err := benchReadJSON(filepath.Join(r.dir, "run.json"), &raw); err != nil {
					t.Fatal(err)
				}
				delete(raw, "subject")
				if err := benchJSON(filepath.Join(r.dir, "run.json"), raw); err != nil {
					t.Fatal(err)
				}
				blobPath, err := benchObjectPath("blob", r.Blobs["prompts"][0])
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(blobPath); err != nil {
					t.Fatal(err)
				}
			} else if corrupt != "store" {
				if err := os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if corrupt == "store" {
				if err := benchJSON(filepath.Join(benchDir(), "store.json"), map[string]int{"schema_version": 99}); err != nil {
					t.Fatal(err)
				}
			}
			before := benchFileHashes(t, benchDir())
			original := benchFileHashes(t, source)
			code, out := h.run("bench", "key", "rescore", "fixture", r.ID, "--key", k.ID)
			if code == 0 || !strings.Contains(out, "isolated rescore validation failed") {
				t.Fatalf("validation accepted %s: %d %s", corrupt, code, out)
			}
			if !reflect.DeepEqual(before, benchFileHashes(t, benchDir())) {
				t.Fatal("failed validation changed source store")
			}
			benchAssertOriginals(t, source, original)
		})
	}
}

func TestBenchScoreLegacyMigrationProposalIdempotent(t *testing.T) {
	h, r, _, source := benchCopiedFixture(t)
	// Schema 1 snapshots often have no captured subject. Reconstruction must
	// put new snapshot data in the proposal, not backfill the old snapshot.
	if err := os.Remove(filepath.Join(r.dir, "snapshot", "subject.json")); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := benchReadJSON(filepath.Join(r.dir, "run.json"), &raw); err != nil {
		t.Fatal(err)
	}
	raw["schema_version"] = json.RawMessage("1")
	delete(raw, "subject")
	raw["future_field"] = json.RawMessage(`{"preserved":true}`)
	if err := benchJSON(filepath.Join(r.dir, "run.json"), raw); err != nil {
		t.Fatal(err)
	}
	var store map[string]any
	if err := benchReadJSON(filepath.Join(benchDir(), "store.json"), &store); err != nil {
		t.Fatal(err)
	}
	store["schema_version"] = 1
	if err := benchJSON(filepath.Join(benchDir(), "store.json"), store); err != nil {
		t.Fatal(err)
	}
	before := benchFileHashes(t, benchDir())
	sourceBefore := benchFileHashes(t, source)
	h.must("bench", "migrate", "--suite", "fixture")
	benchAssertOriginals(t, benchDir(), before)
	first := benchFileHashes(t, benchDir())
	h.now = h.now.Add(24 * time.Hour)
	h.must("bench", "migrate", "--suite", "fixture")
	if !reflect.DeepEqual(first, benchFileHashes(t, benchDir())) {
		t.Fatal("migration duplicated or changed artifacts")
	}
	files, _ := filepath.Glob(filepath.Join(r.dir, "migrations", "*.json"))
	if len(files) != 1 {
		t.Fatalf("migration proposals: %v", files)
	}
	var proposal benchMigration
	if err := benchReadJSON(files[0], &proposal); err != nil {
		t.Fatal(err)
	}
	if len(proposal.SnapshotAdditions["subject.json"]) == 0 {
		t.Fatal("reconstructed snapshot data missing from proposal")
	}
	if benchExists(filepath.Join(r.dir, "snapshot", "subject.json")) {
		t.Fatal("migration backfilled historical snapshot")
	}
	if proposal.OriginalSHA256 != before[filepath.Join("fixture", "runs", r.ID, "run.json")] || !strings.Contains(string(proposal.Run["future_field"]), "preserved") {
		t.Fatalf("lost original hash or unknown fields: %+v", proposal)
	}
	unchanged, err := benchRunLoad("fixture", r.ID)
	if err != nil || unchanged.Subject != "" || unchanged.SchemaVersion != 1 {
		t.Fatalf("original was rebound: %+v %v", unchanged, err)
	}
	out := h.must("bench", "report", "fixture", "--key", "1")
	wantContains(t, out, "subject: not recorded", "not reproducible: subject not recorded", "Old recorded", "New scores:")
	benchAssertOriginals(t, source, sourceBefore)
}

func TestBenchScoreLegacyUnboundAndHashOnlyPromptsStayUnknown(t *testing.T) {
	h, r, k, _ := benchCopiedFixture(t)
	// These fixture edits model a legacy store, before taking preservation hashes.
	var raw map[string]json.RawMessage
	if err := benchReadJSON(filepath.Join(r.dir, "run.json"), &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "subject")
	raw["prompts_stored"] = json.RawMessage("false")
	if err := benchJSON(filepath.Join(r.dir, "run.json"), raw); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(benchDir(), "fixture", "keys", k.ID+".json")
	var keyRaw map[string]json.RawMessage
	if err := benchReadJSON(keyPath, &keyRaw); err != nil {
		t.Fatal(err)
	}
	delete(keyRaw, "subject")
	if err := benchJSON(keyPath, keyRaw); err != nil {
		t.Fatal(err)
	}
	promptID := r.Blobs["prompts"][0]
	promptPath, err := benchObjectPath("blob", promptID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(promptPath); err != nil {
		t.Fatal(err)
	}
	before := benchFileHashes(t, benchDir())
	out := h.must("bench", "key", "rescore", "fixture", r.ID, "--key", k.ID)
	var artifact benchScoreArtifact
	if err := json.Unmarshal([]byte(out), &artifact); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(artifact.Warnings["reproducibility"], " "), "legacy unbound subject: unknown") {
		t.Fatal(artifact.Warnings)
	}
	if artifact.Score.KeySubject != "" || artifact.Score.KeyAgree["group"].Supported != 0 {
		t.Fatalf("manufactured legacy binding or supported answer: %+v", artifact.Score)
	}
	found := false
	for _, input := range artifact.Inputs {
		if input.ID == promptID {
			found = true
			if input.State != "unknown" || input.SHA256 != "" {
				t.Fatalf("manufactured prompt bytes: %+v", input)
			}
		}
	}
	if !found {
		t.Fatal("hash-only prompt identity missing")
	}
	benchAssertOriginals(t, benchDir(), before)
}

func TestBenchScoreReportsAndKeyAddPreserveEvidence(t *testing.T) {
	h, r, k, source := benchCopiedFixture(t)
	old := benchScore{Classes: map[string]locationScore{"duplicate": {Match: 7, Total: 9}}}
	if err := benchImmutableJSON(filepath.Join(r.dir, "score-"+k.ID+".json"), old); err != nil {
		t.Fatal(err)
	}
	if err := benchMkdir(filepath.Join(benchDir(), "fixture", "reports")); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, benchDir(), map[string]string{"README.md": "Historical generated guide\n", "fixture/reports/index.md": "Historical reports\n- [old](old.md)\n", "fixture/reports/old.md": "Old immutable report\n"})
	before := benchFileHashes(t, benchDir())
	sourceBefore := benchFileHashes(t, source)
	index := filepath.Join("fixture", "reports", "index.md")
	beforeIndex := string(mustRead(t, filepath.Join(benchDir(), index)))
	delete(before, index)
	out := h.must("bench", "report", "fixture", "--key", k.ID, "--save")
	wantContains(t, out, "Old recorded duplicate score (legacy, unversioned): 7/9", "New scores: "+benchScorerSchema+" / "+benchScorerProfile)
	benchAssertOriginals(t, benchDir(), before)
	afterIndex := string(mustRead(t, filepath.Join(benchDir(), index)))
	if !strings.HasPrefix(afterIndex, beforeIndex) || strings.Count(afterIndex, "history-") != 2 {
		t.Fatalf("index lost original entries or duplicated new entry: %q", afterIndex)
	}
	t.Logf("index before:\n%sindex after:\n%s", beforeIndex, afterIndex)
	first := benchFileHashes(t, benchDir())
	h.now = h.now.Add(time.Hour)
	h.must("bench", "report", "fixture", "--key", k.ID, "--save")
	if !reflect.DeepEqual(first, benchFileHashes(t, benchDir())) {
		t.Fatal("saved report is not idempotent")
	}
	h.must("bench", "compare", "fixture", r.ID, r.ID, "--key", k.ID, "--md")
	h.must("bench", "report", "fixture", "--json", "--key", k.ID)
	if !reflect.DeepEqual(first, benchFileHashes(t, benchDir())) {
		t.Fatal("read-only report/compare mutated store")
	}
	// Adding a key must not rewrite an existing key, run, prompt or score.
	h.must("bench", "key", "add", "fixture", filepath.Join(h.tmp, "key.json"), "--note", "new key", "--from-run", r.ID, "--from-reference")
	benchAssertOriginals(t, benchDir(), first)
	// Automatic recording and the explicit readme command preserve a guide
	// generated by an older binary. New suite definitions are proposals only.
	h.must("bench", "readme")
	benchAssertOriginals(t, benchDir(), first)
	beforeRecord := benchFileHashes(t, benchDir())
	dir := benchFixture(t, h, "new-arm", 24, false)
	h.must("bench", "record", dir, "--suite", "fixture", "--label", "new-arm", "--arm", "new-arm")
	benchAssertOriginals(t, benchDir(), beforeRecord)
	proposals, err := filepath.Glob(filepath.Join(benchDir(), "fixture", "reports", "suite-proposals", "*.toml"))
	if err != nil || len(proposals) != 1 {
		t.Fatalf("suite definition proposals: %v %v", proposals, err)
	}
	beforeRepeat := benchFileHashes(t, benchDir())
	h.must("bench", "record", dir, "--suite", "fixture", "--label", "new-arm", "--arm", "new-arm")
	if !reflect.DeepEqual(beforeRepeat, benchFileHashes(t, benchDir())) {
		t.Fatal("repeat recording changed existing objects")
	}
	benchAssertOriginals(t, source, sourceBefore)
}

func TestBenchScoreCopyNeverReplacesExistingBytes(t *testing.T) {
	t.Setenv("ORCH_BENCH_DIR", t.TempDir())
	source := t.TempDir()
	writeFiles(t, source, map[string]string{"run.json": "new bytes"})
	target := filepath.Join(benchDir(), "copy")
	if err := benchMkdir(target); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, target, map[string]string{"run.json": "historical bytes"})
	before := benchFileHashes(t, benchDir())
	if err := benchCopy(source, target); err == nil {
		t.Fatal("copy replaced historical bytes")
	}
	benchAssertOriginals(t, benchDir(), before)
	if err := os.WriteFile(filepath.Join(source, "run.json"), []byte("historical bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := benchCopy(source, target); err != nil {
		t.Fatal(err)
	}
	benchAssertOriginals(t, benchDir(), before)
}
