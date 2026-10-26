package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func benchHarness(t *testing.T) *harness {
	t.Helper()
	t.Setenv("ORCH_BENCH_DIR", t.TempDir())
	return setup(t)
}
func benchFixture(t *testing.T, h *harness, label string, seconds int, captured bool) string {
	t.Helper()
	dir := filepath.Join(h.tmp, label)
	for _, sub := range []string{"dossier", "passes"} {
		if e := os.MkdirAll(filepath.Join(dir, sub), 0700); e != nil {
			t.Fatal(e)
		}
	}
	text := string(mustRead(t, filepath.Join(h.repo, "spec.md")))
	rec := gateRecord{Spec: "spec.md", SpecRel: "spec.md", Hash: specHash([]byte(text)), At: h.now.Unix(), SchemaVersion: 2, State: "experiment", Experiment: label, Groups: map[string][]string{"group:api": {"api"}}, PassSeconds: map[string]int{"group:api": seconds}, PassTokens: map[string]passTokens{"group:api": {Requests: 13, Input: 100, Output: 20, CacheRead: 800000, MaxContext: 107000, Cost: .023}}, Findings: []gateFinding{{Where: "package api", Claim: "Save a record", Source: "reuse", Evidence: "internal/store.go:3", Verified: true}}, BenchInputs: benchInputs{Repo: h.repo, SpecPath: "spec.md", SpecHash: benchHash([]byte(text))}}
	b, _ := json.Marshal(rec)
	writeFiles(t, dir, map[string]string{"gate.json": string(b), "report.md": "# Gate\nReview state: **experiment** (schema 2)\n", "passes/group-api.prompt": "Exact prompt\n", "dossier/api.md": "A durable dossier\n", "items.json": `[{"id":"api#1","package":"api","text":"Save a record","kind":"builds"}]`, "checklist.json": `{}`})
	if captured {
		if e := benchWriteGateSubject(dir, h.repo, filepath.Join(h.repo, "spec.md"), text, h.now, mustBenchStamp(t, h.repo), nil); e != nil {
			t.Fatal(e)
		}
	}
	return dir
}
func mustBenchStamp(t *testing.T, repo string) checkoutStamp {
	t.Helper()
	s, e := checkoutFingerprint(repo)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func benchRecordFixture(t *testing.T, h *harness, label string, seconds int, captured bool) *benchRun {
	t.Helper()
	dir := benchFixture(t, h, label, seconds, captured)
	r, e := benchRecord(h.a, dir, "fixture", label, label)
	if e != nil {
		t.Fatal(e)
	}
	h.now = h.now.Add(time.Minute)
	return r
}
func benchKeyFixture(t *testing.T, h *harness, run *benchRun) *benchKey {
	t.Helper()
	path := filepath.Join(h.tmp, "key.json")
	writeFiles(t, h.tmp, map[string]string{"key.json": `[{"package":"api","item_id":"api#1","item":"Save a record","verdict":"duplicate","candidate":"internal/store.go:3"},{"package":"outside","item_id":"outside#1","item":"Other package","verdict":"duplicate","candidate":"other.go:8"}]`})
	k, e := benchKeyAdd(h.a, "fixture", path, "fixture provenance", "Blind review prompt", true, benchKeyOpts{Subject: run.Subject})
	if e != nil {
		t.Fatal(e)
	}
	return k
}
func TestBenchSecurityNamesAndContainment(t *testing.T) {
	t.Setenv("ORCH_BENCH_DIR", t.TempDir())
	for _, name := range []string{"../escape", "a..b", "..", ".hidden", "_inbox", "a/b", "a\\b", strings.Repeat("x", 65), ""} {
		if benchName(name) == nil {
			t.Errorf("accepted %q", name)
		}
	}
	for _, name := range []string{"A", "a.B-9_c", strings.Repeat("x", 64)} {
		if e := benchName(name); e != nil {
			t.Error(e)
		}
	}
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(benchDir(), "suite")); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{filepath.Join(benchDir(), "suite", "new", "run.json"), filepath.Join(benchDir(), "..", "escape")} {
		if e := benchJSON(p, map[string]string{"bad": "write"}); e == nil {
			t.Fatalf("wrote unsafe path %s", p)
		}
	}
	ds, _ := os.ReadDir(outside)
	if len(ds) != 0 {
		t.Fatal("outside directory changed")
	}
	if e := os.Symlink(filepath.Join(outside, "missing"), filepath.Join(benchDir(), "broken")); e != nil {
		t.Fatal(e)
	}
	if benchSafe(filepath.Join(benchDir(), "broken", "new")) == nil {
		t.Fatal("accepted dangling outside symlink")
	}
}
func TestBenchRepoSuiteIgnoredByCLIAndDoctor(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	if e := os.MkdirAll(filepath.Join(h.repo, ".agents"), 0700); e != nil {
		t.Fatal(e)
	}
	writeFiles(t, h.repo, map[string]string{".agents/orchestrate.toml": "bench_suite = '../../outside'\n"})
	cfg, e := loadConfig(h.repo)
	if e != nil {
		t.Fatal(e)
	}
	if cfg.UserBenchSuite != "" || !cfg.benchSuiteIgnored {
		t.Fatalf("config: %+v", cfg)
	}
	out := h.must("doctor")
	if strings.Count(out, "repo bench_suite ignored") != 1 {
		t.Fatal(out)
	}
	dir := benchFixture(t, h, "legacy", 4, false)
	if code, out := h.run("bench", "record", dir); code == 0 || !strings.Contains(out, "--suite is required") {
		t.Fatalf("repo config used: %d %s", code, out)
	}
	writeFiles(t, h.tmp, map[string]string{"config.toml": "bench_suite = 'UserSuite'\n"})
	out = h.must("bench", "record", dir)
	wantContains(t, out, "UserSuite/")
	out = h.must("bench", "record", dir, "--suite", "Explicit")
	wantContains(t, out, "Explicit/")
}
func TestBenchObjectsImmutableAndVerified(t *testing.T) {
	t.Setenv("ORCH_BENCH_DIR", t.TempDir())
	data := []byte("private bytes\n")
	id := benchHash(data)
	if e := objectPut("blob", id, data); e != nil {
		t.Fatal(e)
	}
	if e := objectPut("blob", id, data); e != nil {
		t.Fatal(e)
	}
	if objectPut("blob", id, []byte("different")) == nil {
		t.Fatal("accepted wrong hash")
	}
	path, _ := benchObjectPath("blob", id)
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0600 {
		t.Fatal(fi.Mode())
	}
	if e := os.WriteFile(path, []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := objectGet("blob", id); e == nil {
		t.Fatal("corruption not detected")
	}
	if benchImmutable(path, data) == nil {
		t.Fatal("overwrote immutable object")
	}
}
func TestBenchCanonicalItemsAndDescription(t *testing.T) {
	t.Setenv("ORCH_BENCH_DIR", t.TempDir())
	text := planHeader + "### api: API\nkind: ship\nowns: api.go\n\nBuild API.\n#### Builds\n- Save a record.\n#### Acceptance\nTests pass.\n" + planTail
	now := time.Unix(123, 0)
	v, a := benchSpecFromText(text, "spec.md", benchRepo{Name: "fixture"}, now)
	_, b := benchSpecFromText(text, "spec.md", benchRepo{Name: "fixture"}, now.Add(time.Hour))
	if a.ID != b.ID || len(a.Items) != 1 || a.Items[0].ID != "api#1" || a.Items[0].Text != "Save a record." {
		t.Fatalf("itemsets: %+v %+v", a, b)
	}
	if e := benchSaveSpec(v, a, text); e != nil {
		t.Fatal(e)
	}
	if e := benchSaveSpec(v, b, text); e != nil {
		t.Fatal(e)
	}
	files, _ := os.ReadDir(filepath.Join(benchDir(), "objects", "itemset"))
	if len(files) != 1 {
		t.Fatal(files)
	}
	s := benchSubject{Repo: benchRepo{Name: "fixture"}, Dirty: benchDirty{State: "unknown"}, Spec: benchSpecRef{Hash: "0123456789abcdef", Path: "spec.md", State: "unknown"}}
	want := "fixture commit not recorded; working tree state not recorded; spec spec.md version 0123456789abcdef (unknown)."
	if got := benchDescribe(s, nil); got != want {
		t.Fatalf("description\n got: %s\nwant: %s", got, want)
	}
}
func TestBenchSubjectFactFingerprint(t *testing.T) {
	t.Setenv("ORCH_BENCH_DIR", t.TempDir())
	base := benchSubject{
		Repo: benchRepo{Name: "repo", Path: "/repo", Origin: "https://example.test/repo.git", CommonDir: "/repo/.git"},
		Spec: benchSpecRef{Hash: "0123456789abcdef"}, Commit: "1234567890abcdef1234567890abcdef12345678",
		Dirty:       benchDirty{State: "modified", Untracked: []string{"b", "a"}, Modified: []string{"y", "x"}, DiffSHA256: strings.Repeat("a", 64)},
		CommitBasis: "note", CommitEvidence: "recorded evidence",
	}
	want := "s-0123456789abcdef-1234567-dirty-12225e"
	if id := benchSubjectID(base); id != want {
		t.Fatalf("subject id: %s, want %s", id, want)
	}
	changes := []struct {
		name  string
		apply func(*benchSubject)
	}{
		{"repo name", func(s *benchSubject) { s.Repo.Name = "other" }},
		{"repo path", func(s *benchSubject) { s.Repo.Path = "/other" }},
		{"repo origin", func(s *benchSubject) { s.Repo.Origin = "https://example.test/other.git" }},
		{"repo common dir", func(s *benchSubject) { s.Repo.CommonDir = "/other/.git" }},
		{"dirty state", func(s *benchSubject) { s.Dirty.State = "unknown" }},
		{"untracked paths", func(s *benchSubject) { s.Dirty.Untracked = []string{"other"} }},
		{"modified paths", func(s *benchSubject) { s.Dirty.Modified = []string{"other"} }},
		{"diff hash", func(s *benchSubject) { s.Dirty.DiffSHA256 = strings.Repeat("b", 64) }},
		{"commit basis", func(s *benchSubject) { s.CommitBasis = "user" }},
		{"commit evidence", func(s *benchSubject) { s.CommitEvidence = "other evidence" }},
	}
	for _, change := range changes {
		s := base
		change.apply(&s)
		if benchSubjectID(s) == want {
			t.Errorf("%s did not change the id", change.name)
		}
	}
	same := base
	same.Dirty.Untracked = []string{"a", "b"}
	same.Dirty.Modified = []string{"x", "y"}
	same.Dirty.DiffBlob = strings.Repeat("b", 64)
	same.Dirty.DiffBytes = 123
	same.CreatedAt = time.Unix(123, 0)
	same.CreatedBy = benchBuild{Version: "other"}
	same.Description = "other description"
	if benchSubjectID(same) != want {
		t.Fatal("path order or storage metadata changed the id")
	}
	if !reflect.DeepEqual(base.Dirty.Untracked, []string{"b", "a"}) || !reflect.DeepEqual(base.Dirty.Modified, []string{"y", "x"}) {
		t.Fatal("fingerprinting changed the source lists")
	}
	empty := base
	empty.Commit = ""
	empty.Dirty = benchDirty{State: "unknown"}
	id := benchSubjectID(empty)
	empty.Dirty.Untracked = []string{}
	empty.Dirty.Modified = []string{}
	if benchSubjectID(empty) != id || !strings.HasPrefix(id, "s-0123456789abcdef-nocommit-") {
		t.Fatal("empty path lists or nocommit prefix changed")
	}
	if e := benchName(id); e != nil {
		t.Fatal(e)
	}
}

func TestBenchSubjectEvidenceCanCoexist(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	r := benchRecordFixture(t, h, "source", 2, false)
	first := benchKeyFixture(t, h, r)
	writeFiles(t, h.tmp, map[string]string{"status.txt": "?? historical.txt\n"})
	h.must("bench", "key", "add", "fixture", filepath.Join(h.tmp, "key.json"), "--note", "historical status evidence", "--from-run", r.ID, "--git-status", filepath.Join(h.tmp, "status.txt"))
	second, e := benchKeyLoad("fixture", "2")
	if e != nil {
		t.Fatal(e)
	}
	if first.Subject == second.Subject {
		t.Fatal("different dirty evidence reused a subject id")
	}
	old, e := benchSubjectLoad(first.Subject)
	if e != nil {
		t.Fatal(e)
	}
	updated, e := benchSubjectLoad(second.Subject)
	if e != nil {
		t.Fatal(e)
	}
	if old.Dirty.State != "unknown" || updated.Dirty.State != "untracked-only" {
		t.Fatalf("dirty evidence changed: %+v %+v", old.Dirty, updated.Dirty)
	}
	stored, e := benchRunLoad("fixture", r.ID)
	if e != nil {
		t.Fatal(e)
	}
	if stored.Subject != first.Subject {
		t.Fatal("key registration changed the run subject")
	}
	h.must("bench", "verify", "--suite", "fixture")
}

func TestBenchCaptureGitStatesAndHistory(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	h.git("add", ".")
	h.git("commit", "-qm", "spec version 1")
	path := filepath.Join(h.repo, "spec.md")
	text := mustRead(t, path)
	head, _ := git(h.repo, "rev-parse", "HEAD")
	s, v, _, e := captureSubject(h.repo, path, text, h.now)
	prefix := "s-" + v.Hash + "-" + head[:7] + "-"
	if e != nil || s.Commit != head || v.State != "committed" || len(v.Commits) != 1 || v.Parent != nil || !strings.HasPrefix(s.ID, prefix) || len(s.ID) != len(prefix)+6 {
		t.Fatalf("capture: %+v %+v %v", s, v, e)
	}
	changed := append(append([]byte{}, text...), []byte("\nA new sentence.\n")...)
	if e := os.WriteFile(path, changed, 0600); e != nil {
		t.Fatal(e)
	}
	s, v, _, e = captureSubject(h.repo, path, changed, h.now)
	if e != nil || v.State != "modified" || v.CommittedHash != specHash(text) || s.Dirty.State != "modified" {
		t.Fatalf("modified: %+v %+v %v", s, v, e)
	}
	untracked := filepath.Join(h.repo, "draft.md")
	if e := os.WriteFile(untracked, text, 0600); e != nil {
		t.Fatal(e)
	}
	_, v, _, e = captureSubject(h.repo, untracked, text, h.now)
	if e != nil || v.State != "untracked" {
		t.Fatalf("untracked: %+v %v", v, e)
	}
	if e := os.Rename(filepath.Join(h.repo, ".git"), filepath.Join(h.tmp, "git-saved")); e != nil {
		t.Fatal(e)
	}
	s, v, _, e = captureSubject(h.repo, path, changed, h.now)
	if e != nil || v.State != "unknown" || s.CommitBasis != "unknown" {
		t.Fatalf("no git: %+v %+v %v", s, v, e)
	}
}
func TestBenchGateCaptureAndAutoRecordWithFakes(t *testing.T) {
	h := benchHarness(t)
	gdir := h.gateRepo(1)
	h.git("add", ".")
	h.git("commit", "-qm", "fixture spec")
	h.sessionPi(gatePasses(nil))
	t.Setenv("HERDR_ENV", "")
	out := h.must("gate", "spec.md", "--only", "api", "--experiment", "captured", "--bench-suite", "fixture")
	wantContains(t, out, "bench: fixture/")
	dir := filepath.Join(gdir, "experiments", "captured")
	var c benchCapture
	if e := json.Unmarshal(mustRead(t, filepath.Join(dir, "subject.json")), &c); e != nil {
		t.Fatal(e)
	}
	head, _ := git(h.repo, "rev-parse", "HEAD")
	if c.Subject.Commit != head {
		t.Fatalf("commit %s want %s", c.Subject.Commit, head)
	}
	r, e := benchRunLoad("fixture", "captured")
	if e != nil {
		t.Fatal(e)
	}
	rec := readRecord(t, dir)
	if r.Inputs.SpecHash != rec.Hash || rec.BenchInputs.SpecHash != rec.Hash || len(r.Inputs.SpecSHA256) != 64 || r.SchemaVersion != 2 {
		t.Fatalf("hashes: %+v %+v", r.Inputs, rec.BenchInputs)
	}
	if code, out := h.run("bench", "verify", "--suite", "fixture"); code != 0 {
		t.Fatal(out)
	}
}
func TestBenchRecordRoundTripSnapshotIdentityAndMissingSource(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	h.git("add", ".")
	h.git("commit", "-qm", "fixture")
	dir := benchFixture(t, h, "recorded", 148, true)
	r, e := benchRecord(h.a, dir, "fixture", "recorded", "code")
	if e != nil {
		t.Fatal(e)
	}
	again, e := benchRecord(h.a, dir, "fixture", "recorded", "code")
	if e != nil || again.ID != r.ID {
		t.Fatalf("idempotency %v %v", again, e)
	}
	if !objectHas("spec", r.Inputs.SpecHash) || r.Itemset == "" || len(r.Blobs["prompts"]) != 1 {
		t.Fatalf("objects: %+v", r)
	}
	writeFiles(t, dir, map[string]string{"checklist.json": `{"api":[]}`})
	other, e := benchRecord(h.a, dir, "fixture", "recorded", "code")
	if e != nil || other.ID == r.ID {
		t.Fatalf("snapshot changed: %v %v", other, e)
	}
	if e := os.Rename(dir, filepath.Join(h.tmp, "source-removed")); e != nil {
		t.Fatal(e)
	}
	out := h.must("bench", "show", "fixture/"+r.ID)
	wantContains(t, out, "tested_on", r.Subject)
	out = h.must("bench", "verify", "--suite", "fixture")
	wantContains(t, out, "0 errors", "raw path missing")
}
func TestBenchHistoricalReconstructionDoesNotGuess(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	h.git("add", ".")
	h.git("commit", "-qm", "v1")
	dir := benchFixture(t, h, "old", 534, false)
	text := mustRead(t, filepath.Join(h.repo, "spec.md"))
	writeFiles(t, h.repo, map[string]string{"spec.md": string(text) + "\nnew version\n"})
	h.git("add", ".")
	h.git("commit", "-qm", "v2")
	r, e := benchRecordWith(h.a, dir, "fixture", "old", "original", benchRecordOpts{Repo: h.repo})
	if e != nil {
		t.Fatal(e)
	}
	s, _ := benchSubjectLoad(r.Subject)
	if s.Commit != "" || s.Dirty.State != "unknown" || !strings.HasPrefix(s.ID, "s-"+s.Spec.Hash+"-nocommit-") || !objectHas("spec", s.Spec.Hash) {
		t.Fatalf("historical %+v", s)
	}
	if _, e := benchRecordWith(h.a, dir, "fixture", "invalid", "original", benchRecordOpts{Repo: h.repo, Commit: "deadbeef", CommitBasis: "user", CommitEvidence: "fixture"}); e == nil {
		t.Fatal("accepted nonexistent commit")
	}
	var rec gateRecord
	if e := json.Unmarshal(mustRead(t, filepath.Join(dir, "gate.json")), &rec); e != nil {
		t.Fatal(e)
	}
	rec.Hash = "0123456789abcdef"
	b, _ := json.Marshal(rec)
	writeFiles(t, dir, map[string]string{"gate.json": string(b), "items.json": `[{"id":"api#1","package":"api","text":"An item from the unrecoverable version","kind":"builds"}]`})
	unknown, e := benchRecord(h.a, dir, "fixture", "lost", "original")
	if e != nil {
		t.Fatal(e)
	}
	subj, _ := benchSubjectLoad(unknown.Subject)
	if subj.Spec.State != "unknown" || len(unknown.Warnings) == 0 || objectHas("spec", rec.Hash) {
		t.Fatalf("unknown: %+v", unknown)
	}
	h.must("bench", "verify", "--suite", "fixture")
	h.must("bench", "export", "fixture", "--out", filepath.Join(h.tmp, "unknown-suite.tar.gz"))
}
func TestBenchSubjectMismatchAndRecordComponentsRefused(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	dir := benchFixture(t, h, "bad", 5, true)
	var c benchCapture
	if e := json.Unmarshal(mustRead(t, filepath.Join(dir, "subject.json")), &c); e != nil {
		t.Fatal(e)
	}
	c.Text += "changed"
	b, _ := json.Marshal(c)
	writeFiles(t, dir, map[string]string{"subject.json": string(b)})
	if _, e := benchRecord(h.a, dir, "fixture", "bad", "arm"); e == nil || !strings.Contains(e.Error(), "does not match") {
		t.Fatalf("mismatch: %v", e)
	}
	good := benchRecordFixture(t, h, "good", 5, false)
	good.Arm = "../../outside"
	if e := benchJSON(filepath.Join(good.dir, "run.json"), good); e != nil {
		t.Fatal(e)
	}
	if _, e := benchRuns("fixture"); e == nil {
		t.Fatal("record's arm was trusted")
	}
}
func TestBenchKeyFormatsCoverageAndSharedScorer(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	r := benchRecordFixture(t, h, "legacy", 534, false)
	k := benchKeyFixture(t, h, r)
	if k.Format != "reference-v1" || k.Author != "unknown" || k.Blind != "unknown" || k.OriginalSHA256 == "" {
		t.Fatalf("key: %+v", k)
	}
	score, e := benchRescore(r, k)
	if e != nil {
		t.Fatal(e)
	}
	if score.Classes["duplicate"].Total != 1 || score.Classes["duplicate"].Match != 0 || score.LegacyClasses["duplicate"].Match != 1 || score.Unavailable == "" {
		t.Fatalf("historical findings must be legacy, not supported behavior judgments: %+v", score)
	}
	wantContains(t, score.Coverage, "1 of 2")
	rec := readRecord(t, filepath.Join(r.dir, "snapshot"))
	ad := loadArm(filepath.Join(r.dir, "snapshot"), rec.Experiment, filepath.Join(r.dir, "snapshot"), &rec)
	ref := resolveReference(benchKeyReferences(&benchKey{Items: k.Items[:1], Itemset: k.Itemset}), ad.items)
	if want := scoreLocations(ad, ref, "group", verdictReuse); !reflect.DeepEqual(score.Classes["duplicate"], want) {
		t.Fatalf("bench and gate scores differ: %+v %+v", score.Classes["duplicate"], want)
	}
	writeFiles(t, h.tmp, map[string]string{"key2.json": `[{"id":"api#1","seeks":"Save a record","class":"variation_site","target":{"kind":"symbol","path":"internal/store.go","symbol":"Put","lines":null}}]`})
	k2, e := benchKeyAdd(h.a, "fixture", filepath.Join(h.tmp, "key2.json"), "blind fixture", "prompt", false, benchKeyOpts{FromRun: r.ID, Author: "Recorded reviewer", Blind: "true", BaseKey: k.ID})
	if e != nil {
		t.Fatal(e)
	}
	if k2.Format != "key-v2" || k2.Items[0].Candidate != "internal/store.go#Put" || k2.ID != "key-2" || k2.BaseKey != "key-1" {
		t.Fatalf("new key: %+v", k2)
	}
	h.must("bench", "key", "rescore", "fixture", r.ID, "--key", "2")
	h.must("bench", "verify", "--suite", "fixture")
}
func TestBenchSpecVersionsVariantAndComparability(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	h.git("add", ".")
	h.git("commit", "-qm", "v1")
	first := mustRead(t, filepath.Join(h.repo, "spec.md"))
	r := benchRecordFixture(t, h, "v1", 534, false)
	writeFiles(t, h.repo, map[string]string{"spec.md": string(first) + "\nDifferent version.\n"})
	h.git("add", ".")
	h.git("commit", "-qm", "v2")
	out := h.must("bench", "spec", "add", "--from-git", h.repo, "spec.md", "--all-versions")
	if strings.Count(out, "spec:") != 2 {
		t.Fatal(out)
	}
	second := mustRead(t, filepath.Join(h.repo, "spec.md"))
	v, e := benchSpecLoad(specHash(second))
	if e != nil || v.Parent == nil || v.Parent.Hash != specHash(first) {
		t.Fatalf("parent: %+v %v", v, e)
	}
	variantText := strings.Replace(string(first), "Code that builds api.", "Code that builds api with a new method.", 1)
	if variantText == string(first) {
		t.Fatal("fixture has no api Builds bullet")
	}
	writeFiles(t, h.tmp, map[string]string{"variant.md": variantText})
	h.must("bench", "spec", "add", filepath.Join(h.tmp, "variant.md"), "--variant-of", specHash(first), "--label", "builds-7")
	variant, _ := benchSpecLoad(specHash([]byte(variantText)))
	if variant.VariantOf != specHash(first) {
		t.Fatal(variant)
	}
	subj, _ := benchSubjectLoad(r.Subject)
	if level, _, refused := compat(r, r, nil); level != "same-subject" || refused {
		t.Fatalf("same subject: %s %t", level, refused)
	}
	commitSubject := *subj
	commitSubject.Commit = "1234567abcdef"
	commitSubject.CommitBasis = "user"
	commitSubject.CommitEvidence = "fixture assertion"
	commitSubject.ID = benchSubjectID(commitSubject)
	commitSubject.Description = benchDescribe(commitSubject, nil)
	if e := benchSaveSubject(commitSubject); e != nil {
		t.Fatal(e)
	}
	commitRun := *r
	commitRun.Subject = commitSubject.ID
	commitRun.ID = "other-commit"
	if level, notes, refused := compat(r, &commitRun, nil); level != "same-spec, other commit" || refused || len(notes) == 0 {
		t.Fatalf("other commit: %s %v %t", level, notes, refused)
	}
	other := *subj
	other.Spec.Hash = variant.Hash
	other.Spec.Path = "variants/builds.md"
	other.ID = benchSubjectID(other)
	if e := benchSaveSubject(other); e != nil {
		t.Fatal(e)
	}
	b := *r
	b.ID = "other"
	b.Subject = other.ID
	level, notes, refuse := compat(r, &b, nil)
	if level != "variant" || refuse || len(notes) == 0 {
		t.Fatalf("compat: %s %v %t", level, notes, refuse)
	}
	if count, known := benchChangedKeyItems(&b, &benchKey{Subject: r.Subject, Items: []benchKeyItem{{Package: "api"}}}); !known || count != 1 {
		t.Fatalf("changed key items: %d %t", count, known)
	}
	other.Repo.Name = "other-repo"
	other.Commit = "abcdef1234567"
	other.ID = benchSubjectID(other)
	if e := benchSaveSubject(other); e != nil {
		t.Fatal(e)
	}
	b.Subject = other.ID
	_, _, refuse = compat(r, &b, nil)
	if !refuse {
		t.Fatal("cross repo allowed")
	}
	b.Groups = map[string]benchGroup{"group:api": r.Groups["group:api"], "group:extra": {Packages: []string{"extra"}}}
	if scope, _, e := benchComparisonScope(benchSuite{crossSubject: true}, []*benchRun{r, &b}, r, nil, false); e != nil || !reflect.DeepEqual(scope, []string{"group:api"}) {
		t.Fatalf("group intersection: %v %v", scope, e)
	}
	if _, _, e := benchComparisonScope(benchSuite{crossSubject: true}, []*benchRun{r, &b}, r, nil, true); e == nil {
		t.Fatal("unequal total groups allowed")
	}
}
func TestBenchVerifyCorruption(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	r := benchRecordFixture(t, h, "legacy", 534, false)
	k := benchKeyFixture(t, h, r)
	h.must("bench", "verify", "--suite", "fixture")
	spec, _ := benchObjectPath("spec", r.Inputs.SpecHash)
	if e := os.WriteFile(spec, []byte("flipped byte"), 0600); e != nil {
		t.Fatal(e)
	}
	migrated, _ := benchRunLoad("fixture", r.ID)
	migrated.Subject = "s-0123456789abcdef-nocommit-000000"
	if e := benchJSON(filepath.Join(r.dir, "run.json"), migrated); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(benchDir(), "fixture", "keys", k.ID+".original.json"), []byte("[]"), 0600); e != nil {
		t.Fatal(e)
	}
	code, out := h.run("bench", "verify", "--suite", "fixture")
	if code != 1 {
		t.Fatalf("verify %d %s", code, out)
	}
	wantContains(t, out, "object hash mismatch", "s-0123456789abcdef-nocommit-000000", "key original sha256 mismatch", "verify: 3 errors")
}
func TestBenchArchiveRoundTripAndTraversal(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	a := benchRecordFixture(t, h, "original", 534, false)
	b := benchRecordFixture(t, h, "arm", 148, false)
	benchKeyFixture(t, h, a)
	before := h.must("bench", "compare", "fixture", a.ID, b.ID, "--md")
	archive := filepath.Join(h.tmp, "suite.tar.gz")
	h.must("bench", "export", "fixture", "--out", archive)
	t.Setenv("ORCH_BENCH_DIR", t.TempDir())
	h.must("bench", "import-archive", archive)
	h.must("bench", "verify", "--suite", "fixture")
	after := h.must("bench", "compare", "fixture", a.ID, b.ID, "--md")
	if before != after {
		t.Fatalf("comparison changed after archive\nbefore %s\nafter %s", before, after)
	}
	h.must("bench", "import-archive", archive)
	bad := filepath.Join(h.tmp, "bad.tar.gz")
	f, e := os.Create(bad)
	if e != nil {
		t.Fatal(e)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	payload := []byte("bad")
	if e := tw.WriteHeader(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Size: int64(len(payload)), Mode: 0600}); e != nil {
		t.Fatal(e)
	}
	_, _ = tw.Write(payload)
	_ = tw.Close()
	_ = gz.Close()
	_ = f.Close()
	if code, _ := h.run("bench", "import-archive", bad); code == 0 {
		t.Fatal("archive traversal accepted")
	}
}
func TestBenchReportReadmeWallClockAndUnknowns(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	a := benchRecordFixture(t, h, "run3", 534, false)
	b := benchRecordFixture(t, h, "arm1", 148, false)
	benchKeyFixture(t, h, a)
	out := h.must("bench", "report", "fixture", "--save")
	wantContains(t, out, "## 0. Subjects", "tested on:", "## 1. Headline", "about 3.6x faster", "key coverage: 1 of 2", "commit unknown", "## 8. Reproduce")
	readme := string(mustRead(t, filepath.Join(benchDir(), "README.md")))
	wantContains(t, readme, "Keys are immutable", "subjects", "import-archive", "Repo bench_suite is ignored")
	if e := benchReadme(); e != nil {
		t.Fatal(e)
	}
	files, _ := filepath.Glob(filepath.Join(benchDir(), "fixture", "reports", "history-*.md"))
	if len(files) != 1 || !benchExists(filepath.Join(benchDir(), "fixture", "reports", "index.md")) {
		t.Fatal(files)
	}
	h.must("bench", "report", "fixture", "--save")
	files, _ = filepath.Glob(filepath.Join(benchDir(), "fixture", "reports", "history-*.md"))
	if len(files) != 1 {
		t.Fatal("identical saved report created a duplicate", files)
	}
	wantContains(t, h.must("bench", "show", "fixture/"+b.ID), "tested_on")
	secs, e := benchConsoleSeconds("22:03:42\n", "output\n22:21:57\n")
	if e != nil || secs != 1095 {
		t.Fatalf("clock %v %v", secs, e)
	}
	if _, e := benchConsoleSeconds("22:03:42", "no end time"); e == nil {
		t.Fatal("guessed console time")
	}
	// An old token schema must not acquire zero-dollar costs or turns.
	dir := benchFixture(t, h, "old-schema", 539, false)
	var rec map[string]any
	if e := json.Unmarshal(mustRead(t, filepath.Join(dir, "gate.json")), &rec); e != nil {
		t.Fatal(e)
	}
	rec["pass_tokens"] = map[string]any{"group:api": map[string]any{"requests": 59, "cache_read": 6100000, "max_context": 174000}}
	data, _ := json.Marshal(rec)
	writeFiles(t, dir, map[string]string{"gate.json": string(data)})
	r, e := benchRecord(h.a, dir, "fixture", "old-schema", "original")
	if e != nil {
		t.Fatal(e)
	}
	g := r.Groups["group:api"]
	if g.Cost.USD != nil || g.Turns != nil || g.Cost.Basis != "none" {
		t.Fatalf("invented fields %+v", g)
	}
	s, _ := benchSuiteLoad("fixture")
	table, e := benchComparison("fixture", s, []*benchRun{r, b}, r, nil, false)
	if e != nil {
		t.Fatal(e)
	}
	var report strings.Builder
	benchPrintTable(&report, table)
	wantContains(t, report.String(), "not recorded", "539 s", "148 s")
}
func TestBenchReportOnlyAndUnknownFlags(t *testing.T) {
	h := benchHarness(t)
	dir := filepath.Join(h.tmp, "0123456789abcdef")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	writeFiles(t, dir, map[string]string{"report.md": "# Gate\nStatus: fail\n99 reuse findings\n"})
	out := h.must("bench", "import", dir, "--suite", "reports", "--label", "old", "--kind", "report-only")
	wantContains(t, out, "reports/")
	r, e := benchRunLoad("reports", "old")
	if e != nil || r.Kind != "report-only" || r.Findings != nil {
		t.Fatalf("report only %+v %v", r, e)
	}
	out = h.must("bench", "report", "reports")
	if strings.Contains(out, "| Reuse findings") {
		t.Fatal("invented report-only findings: " + out)
	}
	h.must("bench", "verify", "--suite", "reports")
	key := filepath.Join(h.tmp, "key.json")
	writeFiles(t, h.tmp, map[string]string{"key.json": `[{"package":"api","item":"api#1","verdict":"new"}]`})
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"bench", "list", "--sutie", "reports"}, "flag provided but not defined: -sutie"},
		{[]string{"bench", "import", dir, "--suite", "reports", "--label", "../bad"}, "invalid benchmark name"},
		{[]string{"bench", "key", "add", "reports", key}, "key add requires --note provenance"},
		{[]string{"bench", "key", "add", "reports", key, "--note", "fixture", "--blind", "maybe"}, "--blind must be true, false or unknown"},
	} {
		if code, out := h.run(tc.args...); code == 0 || !strings.Contains(out, tc.want) {
			t.Errorf("%v: code %d, want %q: %s", tc.args, code, tc.want, out)
		}
	}
}

func TestBenchTwoGroupComparisonGolden(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(7)
	makeRun := func(label string, walls, requests, contexts, cache []int, findings int, costs []float64) *benchRun {
		dir := benchFixture(t, h, label, 1, false)
		rec := readRecord(t, dir)
		rec.Groups = map[string][]string{"group:api+2": {"api", "p1", "p2"}, "group:p3+3": {"p3", "p4", "p5", "p6"}}
		rec.PassSeconds = map[string]int{"group:api+2": walls[0], "group:p3+3": walls[1]}
		rec.PassTokens = nil
		rec.Findings = nil
		for i := range findings {
			rec.Findings = append(rec.Findings, gateFinding{Where: "package api", Source: "reuse", Evidence: fmt.Sprintf("store.go:%d", i+1)})
		}
		data, _ := json.Marshal(rec)
		var raw map[string]any
		_ = json.Unmarshal(data, &raw)
		tokens := map[string]any{}
		for i, g := range []string{"group:api+2", "group:p3+3"} {
			fields := map[string]any{"requests": requests[i], "max_context": contexts[i], "cache_read": cache[i]}
			if costs != nil {
				fields["cost_usd"] = costs[i]
			}
			tokens[g] = fields
		}
		raw["pass_tokens"] = tokens
		data, _ = json.Marshal(raw)
		writeFiles(t, dir, map[string]string{"gate.json": string(data)})
		r, e := benchRecord(h.a, dir, "golden", label, label)
		if e != nil {
			t.Fatal(e)
		}
		h.now = h.now.Add(time.Minute)
		return r
	}
	a := makeRun("run3", []int{534, 539}, []int{83, 59}, []int{206000, 174000}, []int{9300000, 6100000}, 31, nil)
	b := makeRun("arm1", []int{148, 149}, []int{13, 15}, []int{107000, 77000}, []int{800000, 700000}, 10, []float64{.023, .024})
	suite, _ := benchSuiteLoad("golden")
	table, e := benchComparison("golden", suite, []*benchRun{a, b}, a, nil, false)
	if e != nil {
		t.Fatal(e)
	}
	var got strings.Builder
	for _, row := range table.Rows {
		switch row.Metric {
		case "Wall time", "Requests", "Max context", "Cache read", "Cost", "Reuse findings (7 packages)":
			fmt.Fprintf(&got, "%s | %s | %s\n", row.Metric, row.Values[0], row.Values[1])
		}
	}
	want := "Wall time | 534 s and 539 s | 148 s and 149 s\nRequests | 83 and 59 | 13 and 15\nMax context | 206k and 174k | 107k and 77k\nCache read | 9.3M and 6.1M | 800k and 700k\nCost | not recorded | $0.05\nReuse findings (7 packages) | 31 | 10\n"
	if got.String() != want {
		t.Fatalf("comparison golden\n%s\nwant\n%s", got.String(), want)
	}
}

func TestBenchConsoleSessionsPromptCapAndCorrection(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	dir := benchFixture(t, h, "evidence", 12, false)
	if e := os.MkdirAll(filepath.Join(dir, "sessions", "group-api"), 0700); e != nil {
		t.Fatal(e)
	}
	writeFiles(t, filepath.Join(dir, "sessions", "group-api"), map[string]string{"session.jsonl": `{"type":"message","message":{"role":"assistant","content":[{"type":"toolCall","name":"read","arguments":{"path":"spec.md"}}]}}` + "\n"})
	writeFiles(t, h.tmp, map[string]string{"start.txt": "22:03:42\n", "console.txt": "finished\n22:21:57\n"})
	writeFiles(t, dir, map[string]string{"passes/group-api.prompt": strings.Repeat("p", 8*1024*1024+1)})
	r, e := benchRecordWith(h.a, dir, "fixture", "evidence", "arm", benchRecordOpts{Console: filepath.Join(h.tmp, "console.txt"), ConsoleStart: filepath.Join(h.tmp, "start.txt"), WithSessions: true})
	if e != nil {
		t.Fatal(e)
	}
	if r.WallSeconds.Value == nil || *r.WallSeconds.Value != 1095 || r.PromptsStored == nil || *r.PromptsStored || objectHas("blob", r.Blobs["prompts"][0]) {
		t.Fatalf("capture limits %+v", r)
	}
	if r.Groups["group:api"].Exploration == nil || !benchExists(filepath.Join(r.dir, "snapshot", "sessions", "group-api", "session.jsonl")) {
		t.Fatal("sessions not preserved")
	}
	head, _ := git(h.repo, "rev-parse", "HEAD")
	h.must("bench", "migrate", "--suite", "fixture", "--run", r.ID, "--commit", head, "--commit-basis", "user", "--commit-evidence", "fixture assertion")
	unchanged, e := benchRunLoad("fixture", r.ID)
	if e != nil {
		t.Fatal(e)
	}
	if unchanged.Subject != r.Subject || len(unchanged.SubjectHistory) != 0 {
		t.Fatal("correction rewrote historical binding")
	}
	files, e := filepath.Glob(filepath.Join(r.dir, "migrations", "*.json"))
	if e != nil || len(files) != 1 {
		t.Fatalf("correction proposal: %v %v", files, e)
	}
	var proposal benchMigration
	if e := benchReadJSON(files[0], &proposal); e != nil {
		t.Fatal(e)
	}
	b, e := json.Marshal(proposal.Run)
	if e != nil {
		t.Fatal(e)
	}
	var corrected benchRun
	if e := json.Unmarshal(b, &corrected); e != nil {
		t.Fatal(e)
	}
	if corrected.Subject == r.Subject || len(corrected.SubjectHistory) != 1 || !reflect.DeepEqual(corrected.Blobs, r.Blobs) || corrected.WallSeconds.Value == nil || *corrected.WallSeconds.Value != 1095 {
		t.Fatalf("correction proposal lost evidence: %+v", corrected)
	}
	h.must("bench", "verify", "--suite", "fixture")
}

func TestBenchStoredPathsLongNamesAndPrivateLock(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	for _, name := range []string{"Objects", "SUBJECTS", "store.json", "README.md"} {
		if benchSuiteName(name) == nil {
			t.Errorf("reserved suite accepted: %s", name)
		}
	}
	dir := benchFixture(t, h, "long", 1, false)
	label := strings.Repeat("A", 64)
	r, e := benchRecord(h.a, dir, "fixture", label, "code")
	if e != nil {
		t.Fatal(e)
	}
	if len(r.ID) > 64 || r.Label != label {
		t.Fatalf("long label: %+v", r)
	}
	if _, e := benchRuns("fixture"); e != nil {
		t.Fatal(e)
	}
	lock, e := os.Stat(filepath.Join(benchDir(), "fixture", ".lock"))
	if e != nil || lock.Mode().Perm() != 0600 {
		t.Fatalf("lock permissions: %v %v", lock, e)
	}
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(benchDir(), "fixture", "reports")); e != nil {
		t.Fatal(e)
	}
	if code, out := h.run("bench", "report", "fixture", "--save"); code == 0 || !strings.Contains(out, "escapes store") {
		t.Fatalf("unsafe report save: %d %s", code, out)
	}
	ds, _ := os.ReadDir(outside)
	if len(ds) != 0 {
		t.Fatal("report wrote outside store")
	}
	k := benchKeyFixture(t, h, r)
	path := filepath.Join(benchDir(), "fixture", "keys", k.ID+".json")
	k.ID = "../../escape"
	if e := benchJSON(path, k); e != nil {
		t.Fatal(e)
	}
	if _, e := benchKeys("fixture"); e == nil {
		t.Fatal("trusted key id from record")
	}
}

func TestBenchKeyBlindItemsAndMultiplePrompts(t *testing.T) {
	h := benchHarness(t)
	h.gateRepo(1)
	r := benchRecordFixture(t, h, "source", 2, false)
	writeFiles(t, h.tmp, map[string]string{"key.json": `[{"id":"api#1","seeks":"An existing persistence method that writes a record without a new wrapper","class":"duplicate","target":{"kind":"file","path":"internal/store.go"}}]`, "blind.json": `[{"id":"api#1","package":"api","item":"Save a record"}]`, "p1.md": "First exact prompt\n", "p2.md": "Second exact prompt\n"})
	h.must("bench", "key", "add", "fixture", filepath.Join(h.tmp, "key.json"), "--note", "recorded review", "--from-run", r.ID, "--items", filepath.Join(h.tmp, "blind.json"), "--blind", "true", "--prompt", filepath.Join(h.tmp, "p1.md"), "--prompt", filepath.Join(h.tmp, "p2.md"))
	k, e := benchKeyLoad("fixture", "1")
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(k.PromptBlobs, []string{benchHash([]byte("First exact prompt\n")), benchHash([]byte("Second exact prompt\n"))}) {
		t.Fatal(k)
	}
	data, e := objectGet("itemset", k.Itemset)
	if e != nil {
		t.Fatal(e)
	}
	var set benchItemset
	if e := json.Unmarshal(data, &set); e != nil || len(set.Items) != 1 || set.Items[0].Text != "Save a record" || set.Items[0].Kind != "legacy" {
		t.Fatalf("blind items: %+v (%v)", set, e)
	}
	h.must("bench", "verify", "--suite", "fixture")
}

// A throwaway worktree is the same repository as the checkout it came from, or
// every run made in one would be refused as "other repo".
func TestBenchRepoInfoNamesALinkedWorktreeAfterItsMainRepository(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "mainrepo")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@e.com"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(main, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(main, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(main, "add", ".")
	run(main, "commit", "-q", "-m", "a")
	linked := filepath.Join(root, "scratch-wt")
	run(main, "worktree", "add", "-q", "--detach", linked, "HEAD")
	a, b := benchRepoInfo(evalOr(main)), benchRepoInfo(evalOr(linked))
	if a.Name != "mainrepo" || b.Name != "mainrepo" || a.CommonDir != b.CommonDir {
		t.Fatalf("main %+v linked %+v", a, b)
	}
	if b.Path != evalOr(linked) {
		t.Errorf("the checkout path is still the worktree's: %s", b.Path)
	}
}
